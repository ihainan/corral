import { readdirSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const scriptsDir = dirname(fileURLToPath(import.meta.url))
const nodeMajor = Number(process.versions.node.split('.')[0])

// Scripts requiring a newer Node runtime (e.g. module.registerHooks in Node 22).
const MIN_NODE = {
  'test-ask-screen.mjs': 22,
}

const files = readdirSync(scriptsDir)
  .filter((f) => f.startsWith('test-') && f.endsWith('.mjs'))
  .sort()

let passed = 0
let skipped = 0

for (const name of files) {
  const need = MIN_NODE[name]
  if (need && nodeMajor < need) {
    console.log(`跳过 ${name}（需要 Node ${need}，当前 Node ${process.versions.node}）`)
    skipped++
    continue
  }
  const full = join(scriptsDir, name)
  const res = spawnSync(process.execPath, [full], { stdio: 'inherit' })
  if (res.error) {
    console.error(`失败: ${name}: ${res.error.message}`)
    process.exit(1)
  }
  if (res.status !== 0) {
    console.error(`失败: ${name}（退出码 ${res.status ?? res.signal}）`)
    process.exit(res.status ?? 1)
  }
  passed++
}

console.log(`web tests: ${passed} passed, ${skipped} skipped`)
