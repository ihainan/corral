import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const loadTs = async (rel) => {
  const srcPath = fileURLToPath(new URL(rel, import.meta.url))
  const src = readFileSync(srcPath, 'utf8')
  const { outputText } = ts.transpileModule(src, {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
  })
  return import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)
}

const { readAskScreen, reviewAsDrawn } = await loadTs('../src/terminal/askScreen.ts')

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

// 2. Same i42.png review screen with standard Claude Code footer hint at bottom
// (previously hijacked by the question branch before review was checked).
const i42WithHint = i42Screen + '\n\nEnter to select · ↑/↓ to navigate · Esc to cancel'
const r2 = readAskScreen(i42WithHint)
assert.equal(r2.kind, 'review', `expected review when hint is present, got ${r2.kind}`)
assert.equal(r2.answers.length, 2)
assert.equal(reviewAsDrawn(r2), true)

// 3. Wrapped 2-line footer hint on review screen
const i42WrappedHint = i42Screen + '\n\nEnter to select · Tab/Arrow keys to navigate\nEsc to cancel'
const r3 = readAskScreen(i42WrappedHint)
assert.equal(r3.kind, 'review', `expected review with wrapped hint, got ${r3.kind}`)
assert.equal(r3.answers.length, 2)

// 4. Classic review screen with "Ready to submit your answers?" and no │ gutter
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

// 5. Incomplete review (no answers yet)
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

// 6. Multi-line wrapped answer in review
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

// 7. New multi-question screen when an OLD "Review your answers" is still in scrollback above it
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

// 8. Multi-question with single-space separated tabs and ☑ / ✓ glyphs + scroll indicator
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

// 9. False positive guard: prose mentioning "Review your answers" above an unrelated numbered menu
const falseReview = [
  'Review your answers',
  'Please pick a file to edit:',
  '❯ 1. main.go',
  '  2. server.go',
].join('\n')
const r9 = readAskScreen(falseReview)
assert.equal(r9.kind, 'unknown', `expected unknown for unrelated menu, got ${r9.kind}`)

// 10. Review screen with cursor moved to option 2 (Cancel) — used by pickReview
const i42Cursor2 = i42Screen.replace('❯ 1. Submit answers\n  2. Cancel', '  1. Submit answers\n❯ 2. Cancel')
const r10 = readAskScreen(i42Cursor2)
assert.equal(r10.kind, 'review')
assert.equal(r10.options[0].cursor, false)
assert.equal(r10.options[1].cursor, true)

console.log('askScreen: all tests passed')
