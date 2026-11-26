import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/quotaPlan.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const { quotaPlanLabel, pickQuotaPlan, quotaTitleText } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

// Valid official plan names are preserved and normalized.
assert.equal(quotaPlanLabel('Google AI Ultra'), 'Google AI Ultra')
assert.equal(quotaPlanLabel('  Claude  Max 20x '), 'Claude Max 20x')
assert.equal(quotaPlanLabel('ChatGPT Pro'), 'ChatGPT Pro')

// Missing, empty, placeholder, control-char, or overlong values are omitted (never shown as "Unknown" / "未知").
for (const bad of [undefined, null, '', '   ', 'unknown', 'Unknown', 'UNKNOWN', 'none', 'null', 'N/A', 'unspecified', '未知', 'Google\nAI', 'a'.repeat(81)]) {
  assert.equal(quotaPlanLabel(bad), undefined, `expected ${JSON.stringify(bad)} to be omitted`)
}

// pickQuotaPlan prefers shown.plan and falls back to secondary plan only when shown.plan is absent/invalid.
assert.equal(pickQuotaPlan({ plan: 'Google AI Ultra' }, 'Google AI Pro'), 'Google AI Ultra')
assert.equal(pickQuotaPlan({ plan: 'Unknown' }, 'Claude Max'), 'Claude Max')
assert.equal(pickQuotaPlan(undefined, 'ChatGPT Plus'), 'ChatGPT Plus')
assert.equal(pickQuotaPlan({}, undefined), undefined)

// quotaTitleText joins heading and tier with middot when present, and keeps bare title otherwise.
assert.equal(quotaTitleText('Subscription quota', 'Google AI Ultra'), 'Subscription quota · Google AI Ultra')
assert.equal(quotaTitleText('订阅额度', 'ChatGPT Pro'), '订阅额度 · ChatGPT Pro')
assert.equal(quotaTitleText('Subscription quota', undefined), 'Subscription quota')
assert.equal(quotaTitleText('订阅额度', 'unknown'), '订阅额度')

console.log('quotaPlan: all tests passed')
