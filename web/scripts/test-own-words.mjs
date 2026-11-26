import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const apiURL = `data:text/javascript;base64,${Buffer.from(
  'export class ApiError extends Error { constructor(status, code, message, extra = {}) { super(message); this.status = status; this.code = code; this.extra = extra } }',
).toString('base64')}`

const ownSrc = readFileSync(fileURLToPath(new URL('../src/terminal/ownWords.ts', import.meta.url)), 'utf8')
const ownJS = ts
  .transpileModule(ownSrc, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
  })
  .outputText.replace(/from ['"]\.\.\/api['"]/g, `from ${JSON.stringify(apiURL)}`)
const { extractAskRegion, Filled, frame, NotAsShown, ownWords, permissionAmend, questionOwn, wholeRow } = await import(
  `data:text/javascript;base64,${Buffer.from(ownJS).toString('base64')}`
)

const failures = []
const run = async (name, fn) => {
  try {
    await fn()
  } catch (e) {
    failures.push(`${name}: ${e.stack || e.message}`)
  }
}

const makeAsk = ({
  spinner = '✽ Orchestrating… (13m 18s · ↓ 44.1k tokens)',
  tabs = '←  ☒ 登录范围  ☐ 密码登录  ☐ 生产应用  ☐ 写入授权  ✔ Submit  →',
  question = '接入统一登录后，博客原来的账号密码登录怎么处理?',
  opt2 = '全部关闭',
  row4 = 'Type something.',
  cursor = 1,
  footer = 'Enter to select · Tab/Arrow keys to navigate · Esc to cancel',
  bgTimer = '13m 18s',
  extraTail = [],
} = {}) => {
  const cur = (n) => (cursor === n ? '❯ ' : '  ')
  return [
    spinner,
    '────────────────────────────────────────────────────────────',
    tabs,
    '',
    question,
    '',
    `${cur(1)}1. 只留给机构与导入账号 (Recommended)`,
    '     个人一律走统一登录；4 个公众号机构账号和定时导入程序仍用密码。',
    `${cur(2)}2. ${opt2}`,
    '     只能统一登录；机构账号与导入程序需要另想办法。',
    `${cur(3)}3. 全部保留`,
    '     两种方式并存，登录页同时显示。',
    `${cur(4)}4. ${row4}`,
    '────────────────────────────────────────────────────────────',
    '  5. Chat about this',
    '',
    footer,
    '  ● main',
    `  ◯ general-purpose  Run Codex adversarial review of P…  ${bgTimer}`,
    `  2 background tasks running · Run Codex adversarial review  ${bgTimer}`,
    ...extraTail,
  ].join('\n')
}

await run('frameIgnoresBackgroundNoiseInAskQuestion', () => {
  const s1 = makeAsk({ spinner: '✽ Orchestrating… (13m 18s · ↓ 44.1k tokens)', bgTimer: '13m 18s', extraTail: ['  Tip: press Tab'] })
  const s2 = makeAsk({
    spinner: '* Pondering… (13m 35s · ↓ 46.2k tokens)',
    bgTimer: '13m 35s',
    footer: 'Enter to submit · Tab/Arrow keys to navigate · Esc to cancel',
    cursor: 4,
    row4: '机构账号我们后面会做特殊处理',
    extraTail: ['  Opus 5.5 │ Ctx 182.4K │ $12.34 (13m35s)'],
  })
  assert.ok(extractAskRegion(s1) !== null, 'should extract AskUserQuestion region')
  assert.equal(frame(s1, 4), frame(s2, 4), 'background timer/spinner/status/footer changes must not change frame')
})

await run('frameDetectsQuestionOptionAndAnsweredChanges', () => {
  const base = frame(makeAsk(), 4)
  assert.notEqual(frame(makeAsk({ question: '生产应用使用哪个域名?' }), 4), base, 'changed question must differ')
  assert.notEqual(frame(makeAsk({ opt2: '仅关闭普通用户' }), 4), base, 'changed option must differ')
  assert.notEqual(
    frame(makeAsk({ tabs: '←  ☒ 登录范围  ☒ 密码登录  ☐ 生产应用  ☐ 写入授权  ✔ Submit  →' }), 4),
    base,
    'answered tab status must differ',
  )
})

await run('ownWordsSubmitsWhileBackgroundTimerTicks', async () => {
  let step = 0
  let cur = makeAsk({ bgTimer: '13m 18s', spinner: '✽ Orchestrating… (13m 18s)', cursor: 1 })
  const sent = []
  const answer = '机构账号我们后面会做特殊处理'
  const target = {
    key: 'k',
    agent: 'claude',
    screen: async () => {
      step++
      // Simulate background task timer & spinner ticking on every screen read.
      const sec = `13m ${18 + step}s`
      cur = cur
        .replace(/13m \d+s/g, sec)
        .replace(/^(?:✽|\*|✻|·) [^\n]+/m, `${['✽', '*', '✻', '·'][step % 4]} Orchestrating… (${sec})`)
      return { text: cur, digest: `d${step}` }
    },
    reply: async (b) => {
      sent.push({ kind: 'reply', keys: b.keys })
      if (b.keys?.every((k) => k === 'down')) {
        cur = makeAsk({
          bgTimer: `13m ${18 + step}s`,
          cursor: 4,
          footer: 'Enter to submit · Tab/Arrow keys to navigate · Esc to cancel',
        })
      } else if (b.keys?.[0] === 'enter') {
        cur = makeAsk({
          tabs: '←  ☒ 登录范围  ☒ 密码登录  ☐ 生产应用  ☐ 写入授权  ✔ Submit  →',
          question: '生产应用怎么配置?',
          cursor: 1,
          bgTimer: `13m ${18 + step}s`,
        })
      }
    },
    fill: async (b) => {
      sent.push({ kind: 'fill', text: b.text })
      cur = makeAsk({
        bgTimer: `13m ${18 + step}s`,
        cursor: 4,
        row4: b.text,
        footer: 'Enter to submit · Tab/Arrow keys to navigate · Esc to cancel',
      })
    },
  }
  const initial = { text: cur, digest: 'd0' }
  const done = await ownWords(target, initial, questionOwn(4), answer, () => true)
  assert.ok(done.text.includes('生产应用怎么配置?'), 'should advance to next question')
  assert.deepEqual(sent, [
    { kind: 'reply', keys: ['down', 'down', 'down'] },
    { kind: 'fill', text: answer },
    { kind: 'reply', keys: ['enter'] },
  ])
})

await run('ownWordsRejectsWhenQuestionChangedBeforeSubmit', async () => {
  const initial = { text: makeAsk(), digest: 'd0' }
  let called = false
  const target = {
    key: 'k',
    agent: 'claude',
    screen: async () => ({ text: makeAsk({ question: '另一道完全不同的问题?' }), digest: 'd1' }),
    reply: async () => {
      called = true
    },
    fill: async () => {
      called = true
    },
  }
  await assert.rejects(() => ownWords(target, initial, questionOwn(4), '自定义回复', () => true), NotAsShown)
  assert.equal(called, false, 'must not send any keys when question changed')
})

await run('ownWordsRejectsWhenOptionOrTabChangedBeforeSubmit', async () => {
  const initial = { text: makeAsk(), digest: 'd0' }
  for (const changed of [
    makeAsk({ opt2: '另一个选项' }),
    makeAsk({ tabs: '←  ☒ 登录范围  ☒ 密码登录  ☐ 生产应用  ☐ 写入授权  ✔ Submit  →' }),
  ]) {
    let called = false
    const target = {
      key: 'k',
      agent: 'claude',
      screen: async () => ({ text: changed, digest: 'd1' }),
      reply: async () => {
        called = true
      },
      fill: async () => {
        called = true
      },
    }
    await assert.rejects(() => ownWords(target, initial, questionOwn(4), '自定义回复', () => true), NotAsShown)
    assert.equal(called, false)
  }
})

await run('wholeRowUsesLastCursorAndRecognizesTabbedFooter', () => {
  // Stale wrapped "❯ 4." in scrollback must not make bottom single-line "❯ 4." fail.
  const screen = [
    '❯ 4. old wrapped text',
    '     continuation from scrollback',
    '────────────────────────────────────────',
    '←  ☒ A  ☐ B  ✔ Submit  →',
    'Question?',
    '  1. One',
    '  2. Two',
    '  3. Three',
    '❯ 4. my answer',
    'Enter to select · Tab/Arrow keys to navigate · Esc to cancel',
  ].join('\n')
  assert.equal(wholeRow(screen, 4), true)

  const wrapped = screen.replace('❯ 4. my answer\n', '❯ 4. my answer\n     wrapped line\n')
  assert.equal(wholeRow(wrapped, 4), false)
})

await run('permissionAmendFrameIgnoresSpinnerAndAgentTimerButGuardsCommand', () => {
  const makePerm = (cmd, spinner, age) =>
    [
      spinner,
      '────────────────────────────────────────',
      ' Bash command',
      `   ${cmd}`,
      ' Do you want to proceed?',
      ' ❯ 1. Yes',
      '   2. No',
      ' Esc to cancel · Tab to amend',
      '  ● main',
      `  ◯ general-purpose  Run Codex  ${age}`,
    ].join('\n')

  const p1 = makePerm('echo hi', '✽ Orchestrating… (13m 18s)', '13m 18s')
  const p2 = makePerm('echo hi', '* Pondering… (13m 19s)', '13m 19s')
  const p3 = makePerm('rm -rf /', '✽ Orchestrating… (13m 18s)', '13m 18s')
  assert.equal(extractAskRegion(p1), null, 'permission prompt must not be treated as AskUserQuestion')
  assert.equal(frame(p1, 1), frame(p2, 1), 'spinner and agent age must not break permissionAmend frame')
  assert.notEqual(frame(p1, 1), frame(p3, 1), 'command change must still change permissionAmend frame')
  assert.equal(permissionAmend(1, true).keys[0], 'tab')
})

// Verify Wrapped text during fill does not send Enter (throws Filled).
await run('ownWordsDoesNotPressEnterWhenPastedTextWraps', async () => {
  const initial = { text: makeAsk({ cursor: 4 }), digest: 'd0' }
  let enterSent = false
  let filled = false
  const wrappedText = 'wrapped line 1'
  const target = {
    key: 'k',
    agent: 'claude',
    screen: async () => {
      if (!filled) return { text: makeAsk({ cursor: 4, bgTimer: '13m 19s' }), digest: 'd1' }
      return {
        text: makeAsk({ cursor: 4, row4: `${wrappedText}\n     wrapped line 2`, bgTimer: '13m 20s' }),
        digest: 'd2',
      }
    },
    reply: async (b) => {
      if (b.keys?.includes('enter')) enterSent = true
    },
    fill: async () => {
      filled = true
    },
  }
  await assert.rejects(() => ownWords(target, initial, questionOwn(4), wrappedText, () => true), Filled)
  assert.equal(enterSent, false, 'must not send enter when pasted row wrapped')
})

// Verify fake tab-like line inside question body cannot drop earlier question lines.
await run('fakeTabLineInQuestionBodyStillDetectsQuestionChange', async () => {
  const stgBody = 'Delete staging?\n☐ Preview ✔ Submit\nChoose:'
  const prdBody = 'Delete production?\n☐ Preview ✔ Submit\nChoose:'

  // Multi-tab dialog: timer tick keeps frame equal, question change above fake tab line MUST differ.
  const mStg = makeAsk({ question: stgBody, bgTimer: '13m 18s', spinner: '✽ Orchestrating… (13m 18s)' })
  const mTim = makeAsk({ question: stgBody, bgTimer: '13m 19s', spinner: '* Pondering… (13m 19s)' })
  const mPrd = makeAsk({ question: prdBody, bgTimer: '13m 18s', spinner: '✽ Orchestrating… (13m 18s)' })
  assert.ok(extractAskRegion(mStg)?.some((l) => l.includes('Delete staging?')), 'multi-tab region must include question above fake tab line')
  assert.equal(frame(mStg, 4), frame(mTim, 4), 'background timer tick must not change frame')
  assert.notEqual(frame(mStg, 4), frame(mPrd, 4), 'question change above fake tab line must change frame')

  // Lone-tab dialog with fake tab line in body.
  const makeLone = (body, spinner) =>
    [
      spinner,
      '────────────────────────────────────────────────────────────',
      '☐ Action',
      '',
      body,
      '❯ 1. Yes',
      '  2. No',
      '  3. Type something.',
      'Enter to select · ↑/↓ to navigate · Esc to cancel',
    ].join('\n')
  const lStg = makeLone(stgBody, '✽ Orchestrating… (13m 18s)')
  const lTim = makeLone(stgBody, '* Pondering… (13m 19s)')
  const lPrd = makeLone(prdBody, '✽ Orchestrating… (13m 18s)')
  assert.equal(frame(lStg, 3), frame(lTim, 3), 'lone-tab timer tick must not change frame')
  assert.notEqual(frame(lStg, 3), frame(lPrd, 3), 'lone-tab question change above fake tab line must change frame')

  // No rule and no real tab bar above fake tab line: must fall back and still detect question change.
  const makeBare = (body) =>
    [body, '❯ 1. Yes', '  2. No', '  3. Type something.', 'Enter to select · ↑/↓ to navigate · Esc to cancel'].join('\n')
  assert.equal(extractAskRegion(makeBare(stgBody)), null)
  assert.notEqual(frame(makeBare(stgBody), 3), frame(makeBare(prdBody), 3))

  // End-to-end ownWords must refuse before sending any key when staging -> production.
  let called = false
  const target = {
    key: 'k',
    agent: 'claude',
    screen: async () => ({ text: mPrd, digest: 'd1' }),
    reply: async () => {
      called = true
    },
    fill: async () => {
      called = true
    },
  }
  await assert.rejects(() => ownWords(target, { text: mStg, digest: 'd0' }, questionOwn(4), '自定义回复', () => true), NotAsShown)
  assert.equal(called, false, 'must not send any keys when question above fake tab line changed')
})

if (failures.length) {
  console.error('FAIL:\n' + failures.join('\n\n'))
  process.exit(1)
}
console.log('PASS: all ownWords tests passed')
