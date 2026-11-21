import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/terminal/apiError.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const { parseApiError } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

// 1. Real Bedrock + multi-proxy request-id error from user report (#14)
const bedrock =
  "API Error: 400 InvokeModelWithResponseStream: operation error Bedrock Runtime: InvokeModelWithResponseStream, https response error StatusCode: 400, RequestID: 605b70fe-80d0-cb42-92d2-7c4acacfe38b, ValidationException: ..display: Input should be 'summarized', 'omitted' (request id: 202611211429542032214808268d9d6Aqpo0Yfi) (request id: 202611211429524604471748268d9d6lxTN5zC5) (request id: 202611211429522483495388268d9d6oRj2xSSM)"
const p1 = parseApiError(bedrock)
assert.ok(p1)
assert.equal(p1.status, '400')
assert.equal(p1.operation, 'Bedrock Runtime · InvokeModelWithResponseStream')
assert.equal(p1.headline, "ValidationException: ..display: Input should be 'summarized', 'omitted'")
assert.deepEqual(p1.requestIds, [
  '202611211429542032214808268d9d6Aqpo0Yfi',
  '202611211429524604471748268d9d6lxTN5zC5',
  '202611211429522483495388268d9d6oRj2xSSM',
  '605b70fe-80d0-cb42-92d2-7c4acacfe38b',
])

// 2. Anthropic JSON API error
const anthropic = 'API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}'
const p2 = parseApiError(anthropic)
assert.ok(p2)
assert.equal(p2.status, '529')
assert.equal(p2.headline, 'overloaded_error: Overloaded')
assert.deepEqual(p2.requestIds, [])

// 3. Normal assistant message must NOT be parsed as API error
assert.equal(parseApiError('Here is how to fix API Error: 400 in your code.'), null)
assert.equal(parseApiError('API Error: 400\n```json\n{}\n```'), null)

console.log('apiError: all tests passed')
