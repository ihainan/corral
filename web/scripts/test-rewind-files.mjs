import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/rewindFiles.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  isEditTool,
  rewindFileBase,
  rewindEditedFiles,
  formatRewindFiles,
  rewindDialogState,
  REWIND_FILES_MAX_SHOWN,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

// 1. Edit tool identification
for (const name of ['Write', 'Edit', 'MultiEdit', 'NotebookEdit']) {
  assert.equal(isEditTool(name), true, `${name} should be an edit tool`)
}
for (const name of ['Bash', 'Read', 'Grep', 'Glob', 'Agent', 'Task', '']) {
  assert.equal(isEditTool(name), false, `${name} must not be an edit tool`)
}

// 2. Basename extraction
assert.equal(rewindFileBase('/home/user/proj/sub/b.txt'), 'b.txt')
assert.equal(rewindFileBase('a.txt'), 'a.txt')
assert.equal(rewindFileBase('/a/b/c.txt/'), 'c.txt')
assert.equal(rewindFileBase('C:\\repo\\sub\\d.ts'), 'd.ts')
assert.equal(rewindFileBase(''), '')

// 3. Extracting edited files strictly after target user message
const entries = [
  { seq: 1, rev: 1, id: 'u1', role: 'user', parts: [{ kind: 'text', text: 'first' }] },
  {
    seq: 2,
    rev: 1,
    id: 'a1',
    role: 'assistant',
    parts: [{ kind: 'tool', tool: { name: 'Write', detail: '/repo/before.txt', done: true } }],
  },
  { seq: 3, rev: 1, id: 'u2', role: 'user', parts: [{ kind: 'text', text: 'second' }] },
  {
    seq: 4,
    rev: 1,
    id: 'a2',
    role: 'assistant',
    parts: [
      { kind: 'tool', tool: { name: 'Write', detail: '/repo/a.txt', done: true } },
      { kind: 'tool', tool: { name: 'Edit', detail: '/repo/sub/b.txt', done: true } },
      // Errored edit must be excluded
      { kind: 'tool', tool: { name: 'Edit', detail: '/repo/failed.txt', done: true, error: true } },
      // Bash file edits must be excluded (Claude Code Rewind does not undo Bash changes)
      {
        kind: 'tool',
        tool: { name: 'Bash', detail: 'echo hi > bash.txt', done: true, files: [{ path: '/repo/bash.txt', add: 1 }] },
      },
      // Read tool must be excluded
      { kind: 'tool', tool: { name: 'Read', detail: '/repo/read.txt', done: true } },
    ],
  },
  // Already-gone entry must be ignored
  {
    seq: 5,
    rev: 2,
    id: 'a2-gone',
    role: 'assistant',
    gone: true,
    parts: [{ kind: 'tool', tool: { name: 'Write', detail: '/repo/gone.txt', done: true } }],
  },
  { seq: 6, rev: 1, id: 'u3', role: 'user', parts: [{ kind: 'text', text: 'third' }] },
  {
    seq: 7,
    rev: 1,
    id: 'a3',
    role: 'assistant',
    parts: [
      // Duplicate path deduplicated
      { kind: 'tool', tool: { name: 'MultiEdit', detail: '/repo/a.txt', done: true } },
      { kind: 'tool', tool: { name: 'NotebookEdit', detail: '/repo/nb/demo.ipynb', done: true } },
    ],
  },
]

// Rewinding to u2 includes edits from both turn 2 and turn 3, excludes before/failed/bash/read/gone
assert.deepEqual(rewindEditedFiles(entries, 'u2'), [
  { path: '/repo/a.txt', name: 'a.txt' },
  { path: '/repo/sub/b.txt', name: 'b.txt' },
  { path: '/repo/nb/demo.ipynb', name: 'demo.ipynb' },
])

// Rewinding to u3 includes only turn 3 edits
assert.deepEqual(rewindEditedFiles(entries, 'u3'), [
  { path: '/repo/a.txt', name: 'a.txt' },
  { path: '/repo/nb/demo.ipynb', name: 'demo.ipynb' },
])

// Same basename in different directories counts as distinct paths (matching Claude Code)
const dupNameEntries = [
  { seq: 1, rev: 1, id: 'u1', role: 'user', parts: [{ kind: 'text', text: 'first' }] },
  {
    seq: 2,
    rev: 1,
    id: 'a1',
    role: 'assistant',
    parts: [
      { kind: 'tool', tool: { name: 'Write', detail: '/repo/a.txt', done: true } },
      { kind: 'tool', tool: { name: 'Write', detail: '/repo/src/b.txt', done: true } },
      { kind: 'tool', tool: { name: 'Write', detail: '/repo/src/c.txt', done: true } },
      { kind: 'tool', tool: { name: 'Write', detail: '/repo/test/b.txt', done: true } },
      // Same full path edited again does NOT double-count
      { kind: 'tool', tool: { name: 'Edit', detail: '/repo/src/b.txt', done: true } },
    ],
  },
]
const dupFiles = rewindEditedFiles(dupNameEntries, 'u1')
assert.equal(dupFiles.length, 4)
assert.deepEqual(
  dupFiles.map((f) => f.path),
  ['/repo/a.txt', '/repo/src/b.txt', '/repo/src/c.txt', '/repo/test/b.txt'],
)
const dupFmt = formatRewindFiles(dupFiles)
assert.equal(dupFmt.total, 4)
assert.equal(dupFmt.more, 0)
assert.deepEqual(
  dupFmt.shown.map((f) => f.label),
  ['a.txt', 'src/b.txt', 'c.txt', 'test/b.txt'],
)

// Unknown or gone target returns empty list
assert.deepEqual(rewindEditedFiles(entries, 'missing'), [])
assert.deepEqual(rewindEditedFiles(entries, ''), [])

// Turn with only Bash/Read has no restorable code files
const noEditEntries = [
  { seq: 1, rev: 1, id: 'u1', role: 'user', parts: [{ kind: 'text', text: 'run test' }] },
  {
    seq: 2,
    rev: 1,
    id: 'a1',
    role: 'assistant',
    parts: [
      { kind: 'tool', tool: { name: 'Bash', detail: 'rm tmp', done: true, files: [{ path: '/repo/tmp' }] } },
      { kind: 'tool', tool: { name: 'Write', detail: '/repo/err.txt', done: true, error: true } },
    ],
  },
]
assert.deepEqual(rewindEditedFiles(noEditEntries, 'u1'), [])

// 4. File list formatting, truncation, and duplicate basename disambiguation
assert.equal(REWIND_FILES_MAX_SHOWN, 5)
const sevenFiles = [
  { path: '/repo/a.txt', name: 'a.txt' },
  { path: '/repo/src/index.ts', name: 'index.ts' },
  { path: '/repo/web/index.ts', name: 'index.ts' },
  { path: '/repo/c.go', name: 'c.go' },
  { path: '/repo/d.go', name: 'd.go' },
  { path: '/repo/e.go', name: 'e.go' },
  { path: '/repo/f.go', name: 'f.go' },
]
const fmt = formatRewindFiles(sevenFiles)
assert.equal(fmt.total, 7)
assert.equal(fmt.more, 2)
assert.deepEqual(fmt.shown, [
  { label: 'a.txt', path: '/repo/a.txt' },
  { label: 'src/index.ts', path: '/repo/src/index.ts' },
  { label: 'web/index.ts', path: '/repo/web/index.ts' },
  { label: 'c.go', path: '/repo/c.go' },
  { label: 'd.go', path: '/repo/d.go' },
])

const fmtSmall = formatRewindFiles(sevenFiles.slice(0, 2))
assert.equal(fmtSmall.total, 2)
assert.equal(fmtSmall.more, 0)
assert.deepEqual(fmtSmall.shown, [
  { label: 'a.txt', path: '/repo/a.txt' },
  { label: 'index.ts', path: '/repo/src/index.ts' },
])

// 5. Confirmation dialog mode, warning body, and danger button state
// No edited files: option hidden, forced to conversation-only, non-danger
assert.deepEqual(rewindDialogState('conversation', 0), {
  hasCodeOption: false,
  mode: 'conversation',
  danger: false,
  bodyKey: 'rewind.confirmBody',
  confirmKey: 'rewind.confirmTitle',
})
assert.deepEqual(rewindDialogState('code', 0), {
  hasCodeOption: false,
  mode: 'conversation',
  danger: false,
  bodyKey: 'rewind.confirmBody',
  confirmKey: 'rewind.confirmTitle',
})

// With edited files, default conversation-only mode: option shown, non-danger
assert.deepEqual(rewindDialogState('conversation', 2), {
  hasCodeOption: true,
  mode: 'conversation',
  danger: false,
  bodyKey: 'rewind.confirmBody',
  confirmKey: 'rewind.confirmTitle',
})

// With edited files, code restore selected: danger mode + code warning + danger confirm button
assert.deepEqual(rewindDialogState('code', 2), {
  hasCodeOption: true,
  mode: 'code',
  danger: true,
  bodyKey: 'rewind.confirmCodeBody',
  confirmKey: 'rewind.confirmCode',
})

console.log('rewindFiles: all tests passed')
