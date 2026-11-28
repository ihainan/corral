import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/sessionRenamePolicy.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  validateMuxSessionName,
  validateDisplaySessionName,
  isDuplicateMuxName,
  renameOrderEntries,
  tmuxEntryKey,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

// 1. tmux/herdr remote session name validation
assert.equal(validateMuxSessionName('work'), '')
assert.equal(validateMuxSessionName('api_v2-dev'), '')
assert.equal(validateMuxSessionName('a'.repeat(64)), '')
assert.equal(validateMuxSessionName(''), 'empty')
assert.equal(validateMuxSessionName('a'.repeat(65)), 'too_long')
assert.equal(validateMuxSessionName('bad.name'), 'invalid_chars')
assert.equal(validateMuxSessionName('bad:name'), 'invalid_chars')
assert.equal(validateMuxSessionName('bad name'), 'invalid_chars')
assert.equal(validateMuxSessionName('a/b'), 'invalid_chars')
assert.equal(validateMuxSessionName('中文'), 'invalid_chars')

// 2. Display name validation (herdr Corral alias / plain shell)
assert.equal(validateDisplaySessionName('后端开发'), '')
assert.equal(validateDisplaySessionName('api server 1'), '')
assert.equal(validateDisplaySessionName('   '), 'empty')
assert.equal(validateDisplaySessionName('字'.repeat(64)), '')
assert.equal(validateDisplaySessionName('字'.repeat(65)), 'too_long')
assert.equal(validateDisplaySessionName('bad\nname'), 'invalid_chars')
assert.equal(validateDisplaySessionName('bad\x00name'), 'invalid_chars')

// 3. Duplicate checks
assert.equal(isDuplicateMuxName('tmux', 'work', ['work', 'other']), true)
assert.equal(isDuplicateMuxName('tmux', 'work2', ['work', 'other']), false)
assert.equal(isDuplicateMuxName('herdr', 'default', ['']), true)
assert.equal(isDuplicateMuxName('herdr', '', ['default']), true)
assert.equal(isDuplicateMuxName('herdr', 'dev', ['default', 'dev']), true)
assert.equal(isDuplicateMuxName('herdr', 'dev2', ['default', 'dev']), false)

// 4. Tab order migration preserves position of renamed tmux sessions (including #2 duplicates)
{
  const order = [
    ['1|herdr|', 0],
    ['1|tmux|work', 0],
    ['1|tmux|work#2', 0],
    ['1|tmux|work-extra', 0],
    ['2|tmux|work', 0],
  ]
  const res = renameOrderEntries(order, 1, 'tmux', 'work', 'backend')
  assert.equal(res.changed, true)
  assert.deepEqual(res.order, [
    ['1|herdr|', 0],
    ['1|tmux|backend', 0],
    ['1|tmux|backend#2', 0],
    ['1|tmux|work-extra', 0],
    ['2|tmux|work', 0],
  ])
}

// 5. Canonical tmux entry key
assert.equal(tmuxEntryKey(42, 'backend'), '42|tmux|backend')

console.log('sessionRenamePolicy: all tests passed')
