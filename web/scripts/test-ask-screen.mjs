import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { registerHooks } from 'node:module'
import { dirname, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import ts from 'typescript'

// Lightweight TS/TSX loader so we can test askScreen.ts, steadyReply.ts, and
// askCard.tsx directly under Node without a bundler.
const stubModules = new Map([
  ['../i18n', 'export const t = (k) => k; export const hasKey = () => false;'],
  ['./i18n', 'export const t = (k) => k; export const hasKey = () => false;'],
  ['./markdown', 'export const Markdown = () => null;'],
  ['./showTerminal', 'export const ShowTerminal = {};'],
  ['./askText', 'export const choiceText = (s) => ({ text: s }); export const lasting = () => undefined;'],
  ['./ownWords', 'export class Filled extends Error {}; export class MaybeSent extends Error {}; export class NotAsShown extends Error {}; export const ownOK = () => true; export const ownWords = async () => null; export const planChange = () => ({}); export const questionOwn = () => ({});'],
])

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (stubModules.has(specifier) && context.parentURL?.startsWith('file://')) {
      return { url: `stub:${encodeURIComponent(specifier)}`, shortCircuit: true }
    }
    if ((specifier.startsWith('./') || specifier.startsWith('../')) && context.parentURL?.startsWith('file://')) {
      const base = resolve(dirname(fileURLToPath(context.parentURL)), specifier)
      for (const ext of ['', '.ts', '.tsx']) {
        if (existsSync(base + ext)) {
          return { url: pathToFileURL(base + ext).href, shortCircuit: true }
        }
      }
    }
    return nextResolve(specifier, context)
  },
  load(url, context, nextLoad) {
    if (url.startsWith('stub:')) {
      const key = decodeURIComponent(url.slice(5))
      return { format: 'module', source: stubModules.get(key), shortCircuit: true }
    }
    if (url.startsWith('file://') && (url.endsWith('.ts') || url.endsWith('.tsx'))) {
      const src = readFileSync(fileURLToPath(url), 'utf8')
      const { outputText } = ts.transpileModule(src, {
        fileName: fileURLToPath(url),
        compilerOptions: {
          module: ts.ModuleKind.ESNext,
          target: ts.ScriptTarget.ES2022,
          jsx: ts.JsxEmit.ReactJSX,
        },
      })
      return { format: 'module', source: outputText, shortCircuit: true }
    }
    return nextLoad(url, context)
  },
})

const { readAskScreen, reviewAsDrawn } = await import('../src/terminal/askScreen.ts')
const { ApiError } = await import('../src/api.ts')
const { keepShown, steadily, withAlive } = await import('../src/terminal/steadyReply.ts')
const { askOn, pickReview } = await import('../src/terminal/askCard.tsx')

const failures = []
const run = async (name, fn) => {
  try {
    await fn()
  } catch (e) {
    failures.push(`${name}: ${e.message}`)
  }
}

// 1. Exact i42.png layout: Review your answers with │ gutter, multi-line wrapped
// Chinese questions, "↑ 1 more line", and NO "Ready to submit your answers?" line.
const i42Screen = [
  '←  ☒ 今晚范围  ☒ 可见变化  ✔ Submit  →',
  '',
  'Review your answers',
  '',
  '  │ ● 今晚的范围：窗口一前做整库备份（出问题停写、恢复整库、回滚到现版本，中断约',
  '  │   10-15 分钟，丢失备份后的写入但可补录），今晚做窗口一、二、三和窗口四到',
  '  │   pool/off（白名单只观察），不开',
  '  │   pool/on；动手前先用真实备份演练一遍，演练不过就顺延。这样可以吗？',
  '  │ → 可以，按这个范围（推荐）',
  '  │ ● 以下变化写死在这一版代码里，部署后普通用户会看到：金额改美元显示（7.3）；下',
  '  │   线钱包、兑换、订阅、Midjourney、Playground、数据看板页面；模型价格接口要求登',
  '  │   录；CYY 成员不能再改管理员发的密钥；第三阶段起「改账号分组」停用；协议转换对',
  '  │   少数异常请求的报错变化。怎么处理？',
  '  │ → 接受，照常上线',
  '  ↑ 1 more line',
  '❯ 1. Submit answers',
  '  2. Cancel',
].join('\n')

await run('i42Screen', () => {
  const r1 = readAskScreen(i42Screen)
  assert.equal(r1.kind, 'review', `expected review for i42Screen, got ${r1.kind}`)
  assert.equal(r1.incomplete, false)
  assert.equal(r1.answers.length, 2)
  assert.equal(
    r1.answers[0].question,
    '今晚的范围：窗口一前做整库备份（出问题停写、恢复整库、回滚到现版本，中断约10-15 分钟，丢失备份后的写入但可补录），今晚做窗口一、二、三和窗口四到pool/off（白名单只观察），不开pool/on；动手前先用真实备份演练一遍，演练不过就顺延。这样可以吗？',
  )
  assert.equal(r1.answers[0].answer, '可以，按这个范围（推荐）')
  assert.equal(
    r1.answers[1].question,
    '以下变化写死在这一版代码里，部署后普通用户会看到：金额改美元显示（7.3）；下线钱包、兑换、订阅、Midjourney、Playground、数据看板页面；模型价格接口要求登录；CYY 成员不能再改管理员发的密钥；第三阶段起「改账号分组」停用；协议转换对少数异常请求的报错变化。怎么处理？',
  )
  assert.equal(r1.answers[1].answer, '接受，照常上线')
  assert.deepEqual(
    r1.options.map((o) => ({ n: o.n, label: o.label, cursor: o.cursor })),
    [
      { n: 1, label: 'Submit answers', cursor: true },
      { n: 2, label: 'Cancel', cursor: false },
    ],
  )
  assert.equal(reviewAsDrawn(r1), true)
})

// 2. Real Claude Code 2.1.330 review screen (askreal42-2.1.330-review.txt):
// notice │ on ● lines and indented → without │ on answer lines.
await run('real21330Review', () => {
  const real330 = [
    '▎ risky actions and prompt injection before executing, runs the ones it assesses as lower-risk, and',
    '▎ blocks the rest.',
    '▎ https://code.claude.com/docs/en/permission-modes',
    '',
    '❯ plan tonight',
    '',
    '──────────────────────────────────────────────────────────────────────────────────────────────────────',
    '←  ☒ 今晚范围  ☒ 可见变化  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    ' │ ● 今晚的范围：窗口一前做整库备份（出问题停写、恢复整库、回滚到现版本，中断约 10-15',
    ' │   分钟，丢失备份后的写入但可补录），今晚做窗口一、二、三和窗口四到 pool/off（白名单只观察），不开',
    ' │   pool/on；动手前先用真实备份演练一遍，演练不过就顺延。这样可以吗？',
    '   → 可以，按这个范围（推荐）',
    ' │ ● 以下变化写死在这一版代码里，部署后普通用户会看到：金额改美元显示（7.3）；下线钱包、兑换、订阅、Mi',
    ' │   djourney、Playground、数据看板页面；模型价格接口要求登录；CYY 成员不能再改管理员发的密钥；第三阶',
    ' │   段起「改账号分组」停用；协议转换对少数异常请求的报错变化。怎么处理？',
    '   → 接受，照常上线',
    '',
    '❯ 1. Submit answers',
    '  2. Cancel',
  ].join('\n')
  const r = readAskScreen(real330)
  assert.equal(r.kind, 'review', `expected review for real 2.1.330 screen, got ${r.kind}`)
  assert.equal(r.answers.length, 2)
  assert.equal(r.answers[0].answer, '可以，按这个范围（推荐）')
  assert.equal(r.answers[1].answer, '接受，照常上线')
  assert.equal(reviewAsDrawn(r), true)
})

// 3. Review screen with known Claude Code footer hints (including wrap right after "·")
await run('reviewWithKnownHints', () => {
  const r2 = readAskScreen(i42Screen + '\n\nEnter to select · ↑/↓ to navigate · Esc to cancel')
  assert.equal(r2.kind, 'review')
  assert.equal(r2.answers.length, 2)
  assert.equal(reviewAsDrawn(r2), true)

  const r3 = readAskScreen(i42Screen + '\n\nEnter to select · Tab/Arrow keys to navigate ·\nEsc to cancel')
  assert.equal(r3.kind, 'review', `wrapped footer ending with · must parse as review, got ${r3.kind}`)

  const r3b = readAskScreen(i42Screen + '\n\nEnter to select · Tab/Arrow keys to navigate\n· Esc to cancel')
  assert.equal(r3b.kind, 'review', `wrapped footer starting with · must parse as review, got ${r3b.kind}`)

  const r4 = readAskScreen(i42Screen + '\n\nEnter to confirm · Tab to switch questions · n to add notes · Esc to cancel')
  assert.equal(r4.kind, 'review')
})

// 4. Classic review screen with "Ready to submit your answers?" and no │ gutter
await run('classicReview', () => {
  const classicReview = [
    '←  ☒ Database  ☒ Features  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    '  ● Which database?',
    '    → SQLite',
    '  ● Which features?',
    '    → Search, Export',
    '',
    'Ready to submit your answers?',
    '❯ 1. Submit answers',
    '  2. Cancel',
    'Esc to cancel',
  ].join('\n')
  const r4 = readAskScreen(classicReview)
  assert.equal(r4.kind, 'review')
  assert.deepEqual(r4.answers, [
    { question: 'Which database?', answer: 'SQLite' },
    { question: 'Which features?', answer: 'Search, Export' },
  ])
  assert.equal(reviewAsDrawn(r4), true)
})

// 5. Incomplete review (no answers yet)
await run('incompleteReview', () => {
  const incompleteReview = [
    '←  ☐ Scope  ☐ Style  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    'You have not answered all questions',
    '',
    'Ready to submit your answers?',
    '❯ 1. Submit answers',
    '  2. Cancel',
  ].join('\n')
  const r5 = readAskScreen(incompleteReview)
  assert.equal(r5.kind, 'review')
  assert.equal(r5.incomplete, true)
  assert.equal(reviewAsDrawn(r5), true)
})

// 6. Multi-line wrapped answer in review
await run('wrappedAnswerReview', () => {
  const wrappedAnswerReview = [
    '←  ☒ Plan  ☒ Notes  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    '  │ ● Which deployment window',
    '  │   should we use?',
    '  │ → Window 1 with full backup',
    '  │   and dry run first',
    '❯ 1. Submit answers',
    '  2. Cancel',
  ].join('\n')
  const r6 = readAskScreen(wrappedAnswerReview)
  assert.equal(r6.kind, 'review')
  assert.deepEqual(r6.answers, [
    { question: 'Which deployment window should we use?', answer: 'Window 1 with full backup and dry run first' },
  ])
})

// 7. New multi-question screen when an OLD "Review your answers" is still in scrollback above it
await run('newQuestionAfterOldReview', () => {
  const newQuestionAfterOldReview = [
    'Review your answers',
    '  ● Earlier question?',
    '  → Yes',
    '────────────────────────────────',
    '←  ☐ 今晚范围  ☐ 可见变化  ✔ Submit  →',
    '',
    '  │ 今晚的范围：窗口一前做整库备份？',
    '❯ 1. 可以，按这个范围（推荐）',
    '     先做备份再执行',
    '  2. 改范围',
    '  3. Type something.',
    '────────────────────────────────',
    '  4. Chat about this',
    '',
    'Enter to select · ↑/↓ to navigate · Esc to cancel',
  ].join('\n')
  const r7 = readAskScreen(newQuestionAfterOldReview)
  assert.equal(r7.kind, 'question', `expected question after old review, got ${r7.kind}`)
  assert.deepEqual(r7.tabs, [
    { label: '今晚范围', answered: false },
    { label: '可见变化', answered: false },
  ])
  assert.equal(r7.question, '今晚的范围：窗口一前做整库备份？')
  assert.equal(r7.options.length, 3)
  assert.equal(r7.own, 3)
})

// 8. Multi-question with single-space separated tabs and ☑ / ✓ glyphs + scroll indicator
await run('singleSpaceTabs', () => {
  const singleSpaceTabs = [
    '← ☑ Scope ☐ Style ✓ Submit →',
    '',
    'Which style?',
    '↑ 1 more line',
    '❯ 1. Minimal',
    '  2. Detailed',
    '  3. Type something.',
    '  4. Chat about this',
    'Enter to select · Tab/Arrow keys to navigate · Esc to cancel',
  ].join('\n')
  const r8 = readAskScreen(singleSpaceTabs)
  assert.equal(r8.kind, 'question', `expected question for single-space tabs, got ${r8.kind}`)
  assert.deepEqual(r8.tabs, [
    { label: 'Scope', answered: true },
    { label: 'Style', answered: false },
  ])
  assert.equal(r8.question, 'Which style?')
  assert.equal(r8.options.length, 3)
  assert.equal(r8.own, 3)
})

// 9. False positive guard: prose mentioning "Review your answers" above an unrelated numbered menu
await run('falseReview', () => {
  const falseReview = [
    'Review your answers',
    'Please pick a file to edit:',
    '❯ 1. main.go',
    '  2. server.go',
  ].join('\n')
  assert.equal(readAskScreen(falseReview).kind, 'unknown')
})

// 10. Review screen with cursor moved to option 2 (Cancel)
await run('reviewCursor2', () => {
  const i42Cursor2 = i42Screen.replace('❯ 1. Submit answers\n  2. Cancel', '  1. Submit answers\n❯ 2. Cancel')
  const r10 = readAskScreen(i42Cursor2)
  assert.equal(r10.kind, 'review')
  assert.equal(r10.options[0].cursor, false)
  assert.equal(r10.options[1].cursor, true)
})

// --- FIX1 Review Tests ---

// 11. [FIX1 #1] Stale incomplete old review + divider rule + Bash permission prompt (repro/stale.txt)
await run('fix1-1-staleReviewBeforePermission', () => {
  const stale = [
    '←  ☒ Database  ☒ Features  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    ' │ ● Earlier question?',
    '   → Yes',
    '',
    '────────────────────────────────────────────────────────────',
    ' Bash command',
    '',
    '   rm important.txt',
    '   Remove a file',
    '',
    ' Do you want to proceed?',
    ' ❯ 1. Yes',
    '   2. No',
    '',
    ' Esc to cancel',
  ].join('\n')
  assert.equal(readAskScreen(stale).kind, 'unknown', `stale.txt must be unknown, got ${readAskScreen(stale).kind}`)

  // Even if the subsequent menu happens to be Submit answers / Cancel, a divider
  // or non-summary dialog text in the summary region must disqualify it.
  const staleSubmit = [
    '←  ☒ Database  ☒ Features  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    ' │ ● Earlier question?',
    '   → Yes',
    '',
    'Unrelated confirmation dialog',
    '❯ 1. Submit answers',
    '  2. Cancel',
  ].join('\n')
  assert.equal(readAskScreen(staleSubmit).kind, 'unknown')
})

// 12. [FIX1 #2] Reject unknown footer hint text (repro/hint.txt + navigate command history)
await run('fix1-2-strictFooterHints', () => {
  const badConfirmHint = [
    '←  ☒ A  ☒ B  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    ' │ ● Q one?',
    '   → Yes',
    '',
    '❯ 1. Submit answers',
    '  2. Cancel',
    '',
    'Enter to confirm deletion of the production database',
  ].join('\n')
  assert.equal(readAskScreen(badConfirmHint).kind, 'unknown', 'arbitrary Enter to confirm text must be rejected')

  const badNavHint = [
    '←  ☒ A  ☒ B  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    ' │ ● Q one?',
    '   → Yes',
    '',
    '❯ 1. Submit answers',
    '  2. Cancel',
    '',
    '↑/↓ to navigate command history',
  ].join('\n')
  assert.equal(readAskScreen(badNavHint).kind, 'unknown', 'arbitrary navigate text must be rejected')
})

// 13. [FIX1 #3] pickReview must verify answers and options stay identical across steps
await run('fix1-3-pickReviewChecksAnswersUnchanged', async () => {
  const reviewA1 = [
    '←  ☒ Scope  ☒ Risk  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    ' │ ● Deploy tonight?',
    '   → Dry run only',
    '',
    '❯ 1. Submit answers',
    '  2. Cancel',
  ].join('\n')
  // Different review page (different answer!) with cursor already on 2
  const reviewB2 = [
    '←  ☒ Scope  ☒ Risk  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    ' │ ● Deploy tonight?',
    '   → Drop production database',
    '',
    '  1. Submit answers',
    '❯ 2. Cancel',
  ].join('\n')

  const sent = []
  let current = { text: reviewA1, digest: 'd1' }
  const target = {
    key: 'test',
    agent: 'claude',
    screen: async () => current,
    reply: async (b) => {
      sent.push(b.keys)
      // Simulate review page swapping to a different review after "down"
      if (b.keys?.includes('down')) current = { text: reviewB2, digest: 'd2' }
    },
  }
  await assert.rejects(() => pickReview(target, current, 2), 'pickReview must reject when review answers changed')
  assert.deepEqual(sent, [['down']], `must not send enter after answers changed, sent=${JSON.stringify(sent)}`)
})

// 14. [FIX1 #4] steadily and withAlive must check alive() before the first send
await run('fix1-4-aliveCheckedBeforeFirstSend', async () => {
  let calls = 0
  await assert.rejects(
    () =>
      steadily(
        async () => {
          calls++
        },
        async () => ({ text: '', digest: 'x' }),
        { digest: 'd0' },
        () => false,
      ),
    'steadily must reject when alive() is false before first send',
  )
  assert.equal(calls, 0, 'send must not be called when alive() is false')

  // retry=false (used by non-Claude agents and once:true) must still send once when alive,
  // and must NOT retry on screen_changed.
  let nonClaudeCalls = 0
  const cdText = '⚠ Claude Code will automatically deny this request in 1:57, to avoid blocking progress on an unattended session\n❯ 1. Yes\n  2. No'
  keepShown({ text: cdText, digest: 'cd1' })
  await assert.rejects(
    () =>
      steadily(
        async () => {
          nonClaudeCalls++
          throw new ApiError(409, 'screen_changed', 'changed')
        },
        async () => ({ text: cdText.replace('1:57', '1:56'), digest: 'cd2' }),
        { digest: 'cd1' },
        () => true,
        false,
      ),
  )
  assert.equal(nonClaudeCalls, 1, 'retry=false should send once and not retry')

  // Multi-step pickReview: if card becomes unmounted/inactive after "down", "enter" must NOT be sent.
  const review1 = [
    '←  ☒ A  ☒ B  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    ' │ ● Proceed?',
    '   → Yes',
    '',
    '❯ 1. Submit answers',
    '  2. Cancel',
  ].join('\n')
  const review2 = review1.replace('❯ 1. Submit answers\n  2. Cancel', '  1. Submit answers\n❯ 2. Cancel')
  let alive = true
  const keysSent = []
  let scr = { text: review1, digest: 'r1' }
  const rawTarget = {
    key: 'test2',
    agent: 'claude',
    screen: async () => scr,
    reply: async ({ alive: a, ...b }) =>
      steadily(
        async () => {
          keysSent.push(b.keys)
          if (b.keys?.includes('down')) {
            scr = { text: review2, digest: 'r2' }
            alive = false // Card disappears before Enter!
          }
        },
        async () => scr,
        b,
        a,
      ),
  }
  await assert.rejects(() => pickReview(withAlive(rawTarget, () => alive), scr, 2))
  assert.deepEqual(keysSent, [['down']], `must not send enter after alive becomes false, sent=${JSON.stringify(keysSent)}`)
})

// 15. [FIX1 #5] wrappedLabel in askOn must match remaining text against option description
await run('fix1-5-wrappedLabelStrictDescription', () => {
  const dangerousScreen = readAskScreen(
    [
      '☐ Action',
      '',
      'What should we do?',
      '❯ 1. Keep',
      '     files and delete backups',
      '  2. Abort',
      '  3. Type something.',
      '────────────────────────────────',
      '  4. Chat about this',
      '',
      'Enter to select · ↑/↓ to navigate · Esc to cancel',
    ].join('\n'),
  )
  assert.equal(dangerousScreen.kind, 'question')

  // Log says "Keep files" with no description -> MUST NOT match "Keep" + "files and delete backups"!
  const callNoDesc = {
    id: 'c1',
    name: 'AskUserQuestion',
    asks: [{ question: 'What should we do?', options: [{ label: 'Keep files' }, { label: 'Abort' }] }],
  }
  assert.equal(askOn(callNoDesc, dangerousScreen), -1, 'wrappedLabel must reject unmatched trailing text when log has no description')

  // Log says "Keep files" with a DIFFERENT description -> MUST NOT match!
  const callDiffDesc = {
    id: 'c2',
    name: 'AskUserQuestion',
    asks: [
      {
        question: 'What should we do?',
        options: [{ label: 'Keep files', description: 'Leave everything untouched' }, { label: 'Abort' }],
      },
    ],
  }
  assert.equal(askOn(callDiffDesc, dangerousScreen), -1, 'wrappedLabel must reject mismatched description')

  // Genuine wrapped label whose tail matches log's description -> MUST match!
  const genuineScreen = readAskScreen(
    [
      '☐ Action',
      '',
      'What should we do?',
      '❯ 1. Keep all existing',
      '     files in repository Preserve everything untouched',
      '  2. Abort',
      '  3. Type something.',
      '────────────────────────────────',
      '  4. Chat about this',
      '',
      'Enter to select · ↑/↓ to navigate · Esc to cancel',
    ].join('\n'),
  )
  const callGenuine = {
    id: 'c3',
    name: 'AskUserQuestion',
    asks: [
      {
        question: 'What should we do?',
        options: [
          { label: 'Keep all existing files in repository', description: 'Preserve everything untouched' },
          { label: 'Abort' },
        ],
      },
    ],
  }
  assert.equal(askOn(callGenuine, genuineScreen), 0, 'genuine wrapped label with matching description should match')
})

// 16. [FIX1 #6] tabLine must not match ☑ inside question body and must not split "Color ✔ dark"
await run('fix1-6-tabLineBodyAndCheckmarkInLabel', () => {
  const checkboxInBody = [
    '────────────────────────────────────────',
    '←  ☐ Scope  ☐ Confirm  ✔ Submit  →',
    '',
    'We will deploy tonight:',
    '☑ Is this acceptable?',
    '',
    '❯ 1. Yes',
    '  2. No',
    '  3. Type something.',
    '────────────────────────────────────────',
    '  Chat about this',
    '',
    'Enter to select · ↑/↓ to navigate · Esc to cancel',
  ].join('\n')
  const rBody = readAskScreen(checkboxInBody)
  assert.equal(rBody.kind, 'question')
  assert.deepEqual(rBody.tabs, [
    { label: 'Scope', answered: false },
    { label: 'Confirm', answered: false },
  ])
  assert.equal(rBody.question, 'We will deploy tonight: ☑ Is this acceptable?')

  const checkInTabLabel = [
    '────────────────────────────────────────',
    '←  ☐ Color ✔ dark  ☐ Size  ✔ Submit  →',
    '',
    'Pick a theme?',
    '❯ 1. Dark',
    '  2. Light',
    '  3. Type something.',
    '────────────────────────────────────────',
    '  Chat about this',
    '',
    'Enter to select · ↑/↓ to navigate · Esc to cancel',
  ].join('\n')
  const rTab = readAskScreen(checkInTabLabel)
  assert.equal(rTab.kind, 'question')
  assert.deepEqual(rTab.tabs, [
    { label: 'Color ✔ dark', answered: false },
    { label: 'Size', answered: false },
  ])
})

// 17. [FIX1 #7 optional] Normal question literally titled "Review your answers" (repro/titled.txt)
await run('fix1-7-questionTitledReviewYourAnswers', () => {
  const titled = [
    '────────────────────────────────────────',
    '←  ☐ Check  ☐ Next  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    '❯ 1. Yes',
    '  2. No',
    '  3. Type something.',
    '────────────────────────────────────────',
    '  Chat about this',
    '',
    'Enter to select · ↑/↓ to navigate · Esc to cancel',
  ].join('\n')
  const r = readAskScreen(titled)
  assert.equal(r.kind, 'question', `titled.txt should parse as question, got ${r.kind}`)
  assert.equal(r.question, 'Review your answers')
  assert.deepEqual(r.tabs, [
    { label: 'Check', answered: false },
    { label: 'Next', answered: false },
  ])
  assert.equal(r.options.length, 3)
  assert.equal(r.own, 3)
})

// 18. [FIX3 #1] Footer wrapped right after "·" (repro/c1-wrapdot.txt)
await run('fix3-1-wrapAfterDot', () => {
  const wrapDot = [
    '←  ☒ A  ☒ B  ✔ Submit  →',
    '',
    'Review your answers',
    '',
    ' │ ● Q one?',
    '   → Yes',
    '',
    '❯ 1. Submit answers',
    '  2. Cancel',
    '',
    'Enter to select · Tab/Arrow keys to navigate ·',
    'Esc to cancel',
  ].join('\n')
  const r = readAskScreen(wrapDot)
  assert.equal(r.kind, 'review', `c1-wrapdot.txt must parse as review, got ${r.kind}`)
  assert.equal(r.answers.length, 1)
})

// 19. [FIX3 #2] Plan screen with ctrl+g plan file path (with and without spaces)
await run('fix3-2-planFilePathWithSpaces', () => {
  const makePlan = (path) =>
    [
      'Ready to code?',
      '',
      'Would you like to proceed?',
      '❯ 1. Yes, clear context and auto-accept edits',
      '  2. Yes, auto-accept edits',
      '  3. Yes, manually approve edits',
      '  4. Tell Claude what to change',
      '',
      `ctrl+g to edit in Vim · ${path}`,
    ].join('\n')

  const rNoSpace = readAskScreen(makePlan('~/.claude/plans/example.md'))
  assert.equal(rNoSpace.kind, 'plan', `plan path without spaces must parse as plan, got ${rNoSpace.kind}`)
  assert.equal(rNoSpace.options.length, 4)

  const rWithSpace = readAskScreen(makePlan('/home/My Name/.claude/plans/example.md'))
  assert.equal(rWithSpace.kind, 'plan', `plan path with spaces must parse as plan, got ${rWithSpace.kind}`)
  assert.equal(rWithSpace.options.length, 4)
})

// 20. [FIX3 #3] Two tab bars in scrollback with no divider rule between them (repro/c3-oldtabs.txt)
await run('fix3-3-latestTabBarWithoutDivider', () => {
  const oldTabs = [
    '←  ☒ Old  ☒ Earlier  ✔ Submit  →',
    'Review your answers',
    '  ● Old question?',
    '    → Old answer',
    '←  ☐ New  ☐ Next  ✔ Submit  →',
    'New question?',
    '❯ 1. Yes',
    '  2. No',
    '  3. Type something.',
    'Enter to select · ↑/↓ to navigate · Esc to cancel',
  ].join('\n')
  const r = readAskScreen(oldTabs)
  assert.equal(r.kind, 'question', `c3-oldtabs.txt must parse as question, got ${r.kind}`)
  assert.deepEqual(r.tabs, [
    { label: 'New', answered: false },
    { label: 'Next', answered: false },
  ])
  assert.equal(r.question, 'New question?')
})

// 21. [FIX4 #1] Standalone "☐ ..." line inside question body must not hijack multi-tab bar (repro/d1-bodybox.txt)
await run('fix4-1-uncheckedBoxInQuestionBody', () => {
  const bodyBox = [
    '────────────────────────────────────────',
    '←  ☐ Plan  ☐ Next  ✔ Submit  →',
    '',
    'Which items should ship tonight?',
    '☐ Billing page',
    '  the rest waits',
    '',
    '❯ 1. Yes',
    '  2. No',
    '  3. Type something.',
    '────────────────────────────────────────',
    '  Chat about this',
    '',
    'Enter to select · ↑/↓ to navigate · Esc to cancel',
  ].join('\n')
  const r = readAskScreen(bodyBox)
  assert.equal(r.kind, 'question', `d1-bodybox.txt must parse as question, got ${r.kind}`)
  assert.deepEqual(r.tabs, [
    { label: 'Plan', answered: false },
    { label: 'Next', answered: false },
  ])
  assert.equal(r.question, 'Which items should ship tonight? ☐ Billing page the rest waits')

  // A real lone question separated by a divider rule from an older multi-tab dialog stays lone.
  const loneAfterRule = [
    '←  ☒ OldA  ☒ OldB  ✔ Submit  →',
    '────────────────────────────────────────',
    '☐ Solo',
    '',
    'Ship this single change?',
    '❯ 1. Yes',
    '  2. No',
    '  3. Type something.',
    'Enter to select · ↑/↓ to navigate · Esc to cancel',
  ].join('\n')
  const rSolo = readAskScreen(loneAfterRule)
  assert.equal(rSolo.kind, 'question')
  assert.deepEqual(rSolo.tabs, [])
  assert.equal(rSolo.question, 'Ship this single change?')
})

const R = '────────────────────────────────────────────────────────────'
// 22. [review E] The dialog's tab bar is the one right under a rule. A real lone question (Claude Code 2.1.330,
// captured 11-24): its "☐ Ship" header under the rule, a "☐ …" line in its words kept as words; a lone question
// after an old review with no rule between (the latest tab line, as before); the same with the rule.
await run('reviewE-tabBarUnderRule', () => {
  const real = ['❯ plan tonight', 'RULE', ' ☐ Ship', '', 'Which items ship tonight?', '', '❯ 1. Billing page', '     the rest waits', '  2. Nothing', '     wait a day', '  3. Type something.', 'RULE', '  4. Chat about this', '', 'Enter to select · ↑/↓ to navigate · Esc to cancel']
  const r1 = readAskScreen(real.map((l) => (l === 'RULE' ? R : l)).join('\n'))
  assert.equal(r1.kind, 'question')
  assert.deepEqual(r1.tabs, [])
  assert.equal(r1.question, 'Which items ship tonight?')
  const body = ['RULE', '☐ Ship', '', 'Which items ship tonight?', '☐ Billing page', '  the rest waits', '', '❯ 1. Yes', '  2. No', '  3. Type something.', 'RULE', '  Chat about this', '', 'Enter to select · ↑/↓ to navigate · Esc to cancel']
  const r2 = readAskScreen(body.map((l) => (l === 'RULE' ? R : l)).join('\n'))
  assert.equal(r2.kind, 'question')
  assert.deepEqual(r2.tabs, [])
  assert.ok(r2.question.startsWith('Which items ship tonight?'), r2.question)
  const after = ['←  ☒ Old  ☒ Earlier  ✔ Submit  →', 'Review your answers', '  ● Old question?', '    → Old answer', 'RULE?', '☐ New', '', 'Single question?', '❯ 1. Yes', '  2. No', '  3. Type something.', 'Enter to select · ↑/↓ to navigate · Esc to cancel']
  for (const withRule of [false, true]) {
    const r3 = readAskScreen(after.flatMap((l) => (l === 'RULE?' ? (withRule ? [R] : []) : [l])).join('\n'))
    assert.equal(r3.kind, 'question', String(withRule))
    assert.deepEqual(r3.tabs, [], String(withRule))
    assert.equal(r3.question, 'Single question?', String(withRule))
  }
})

if (failures.length) {
  console.error(`askScreen: ${failures.length} test(s) failed:`)
  for (const f of failures) console.error(' -', f)
  process.exit(1)
}
console.log('askScreen: all tests passed')
