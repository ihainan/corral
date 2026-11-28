import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'

const srcPath = fileURLToPath(new URL('../src/linkPolicy.ts', import.meta.url))
const src = readFileSync(srcPath, 'utf8')
const { outputText } = ts.transpileModule(src, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
})
const {
  isAppRoutePath,
  classifyLink,
  isExternalLink,
  shouldOpenInNewTab,
  linkTargetProps,
  isPlainPrimaryClick,
} = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)

const BASE = 'https://corral.example.com/agents?s=abc'

// 1. Cross-origin http(s) URLs are external and default to opening in a new tab with noopener noreferrer.
for (const url of [
  'https://github.com/org/repo/pull/42',
  'http://example.com/docs',
  'https://sub.corral.example.com/',
  'https://corral.example.com:8443/agents',
  'http://corral.example.com/agents', // different scheme -> different origin
  '//other.example.com/path?q=1', // protocol-relative external
  '///other.example.com/path',
]) {
  const info = classifyLink(url, BASE)
  assert.equal(info.kind, 'external-http', `expected external-http for ${url}`)
  assert.equal(info.external, true, `expected external for ${url}`)
  assert.equal(info.openInNewTab, true, `expected openInNewTab for ${url}`)
  assert.equal(info.safe, true, `expected safe for ${url}`)
  assert.equal(isExternalLink(url, BASE), true)
  assert.equal(shouldOpenInNewTab(url, BASE), true)
  assert.deepEqual(linkTargetProps(url, BASE), { target: '_blank', rel: 'noopener noreferrer' })
}

// 2. Same-origin absolute URLs pointing to Corral SPA routes stay in the current window.
for (const [url, wantPath] of [
  ['https://corral.example.com/', '/'],
  ['https://corral.example.com/agents?s=abc&pane=w1:p1', '/agents?s=abc&pane=w1:p1'],
  ['https://CORRAL.EXAMPLE.COM:443/hosts/3/edit', '/hosts/3/edit'],
  ['https://corral.example.com/t/deadbeef#top', '/t/deadbeef#top'],
  ['//corral.example.com/keys', '/keys'],
]) {
  const info = classifyLink(url, BASE)
  assert.equal(info.kind, 'internal-app', `expected internal-app for ${url}`)
  assert.equal(info.external, false, `expected internal for ${url}`)
  assert.equal(info.openInNewTab, false, `expected same-window for ${url}`)
  assert.equal(info.spaPath, wantPath, `expected spaPath for ${url}`)
  assert.equal(isExternalLink(url, BASE), false)
  assert.equal(shouldOpenInNewTab(url, BASE), false)
  assert.deepEqual(linkTargetProps(url, BASE), {})
}

// 3. Relative paths stay in the current window and resolve against the current page URL.
for (const [url, wantPath] of [
  ['/', '/'],
  ['/hosts', '/hosts'],
  ['/hosts/new', '/hosts/new'],
  ['/hosts/12/edit', '/hosts/12/edit'],
  ['/agents', '/agents'],
  ['/t', '/t'],
  ['/t/sess1', '/t/sess1'],
  ['/w/sess1', '/w/sess1'],
  ['/keys', '/keys'],
  ['/settings', '/settings'],
  ['/recordings', '/recordings'],
  ['/recordings/99', '/recordings/99'],
  ['/admin/users', '/admin/users'],
  ['/admin/audit', '/admin/audit'],
  ['./keys', '/keys'],
  ['../settings', '/settings'],
  ['?s=next', '/agents?s=next'],
]) {
  const info = classifyLink(url, BASE)
  assert.equal(info.kind, 'internal-app', `expected internal-app for ${url}`)
  assert.equal(info.external, false)
  assert.equal(info.openInNewTab, false)
  assert.equal(info.spaPath, wantPath)
  assert.deepEqual(linkTargetProps(url, BASE), {})
}

// 4. Hash-only and empty hrefs stay in the current document.
for (const url of ['', '   ', '#section-1']) {
  const info = classifyLink(url, BASE)
  assert.equal(info.kind, 'hash')
  assert.equal(info.external, false)
  assert.equal(info.openInNewTab, false)
  assert.equal(info.safe, true)
  assert.deepEqual(linkTargetProps(url, BASE), {})
}

// 5. Same-origin non-SPA endpoints (static notices, API downloads, OIDC start) are not external,
//    stay native by default, and get target="_blank" + rel="noopener noreferrer" when forceNewTab=true.
for (const url of [
  '/THIRD-PARTY-NOTICES.txt',
  '/api/recordings/42/cast?download=1',
  '/api/auth/oidc/start?next=%2F',
  'https://corral.example.com/THIRD-PARTY-NOTICES.txt',
]) {
  const info = classifyLink(url, BASE)
  assert.equal(info.kind, 'same-origin-other', `expected same-origin-other for ${url}`)
  assert.equal(info.external, false)
  assert.equal(info.openInNewTab, false)
  assert.equal(info.spaPath, undefined)
  assert.deepEqual(linkTargetProps(url, BASE), {})
  assert.deepEqual(linkTargetProps(url, BASE, true), { target: '_blank', rel: 'noopener noreferrer' })
}

// 6. External OS protocols (mailto:, tel:, sms:) count as external (never intercepted by SPA router),
//    but do NOT set target="_blank" (which would spawn an empty blank tab alongside the OS app).
for (const url of ['mailto:support@example.com', 'MAILTO:ops@example.com?subject=hi', 'tel:+8613800138000', 'sms:+15551234']) {
  const info = classifyLink(url, BASE)
  assert.equal(info.kind, 'external-scheme', `expected external-scheme for ${url}`)
  assert.equal(info.external, true)
  assert.equal(info.openInNewTab, false)
  assert.equal(info.safe, true)
  assert.equal(isExternalLink(url, BASE), true)
  assert.equal(shouldOpenInNewTab(url, BASE), false)
  assert.equal(shouldOpenInNewTab(url, BASE, true), false)
  assert.deepEqual(linkTargetProps(url, BASE), {})
  assert.deepEqual(linkTargetProps(url, BASE, true), {})
}

// 7. Unsafe schemes and control-character smuggling are rejected.
for (const url of [
  'javascript:alert(1)',
  'JAVASCRIPT:void(0)',
  'data:text/html,<script>alert(1)</script>',
  'vbscript:msgbox(1)',
  'file:///etc/passwd',
  'https://example.com/\nfoo',
  'http://[invalid',
]) {
  const info = classifyLink(url, BASE)
  assert.equal(info.kind, 'unsafe', `expected unsafe for ${JSON.stringify(url)}`)
  assert.equal(info.safe, false)
  assert.equal(info.openInNewTab, false)
  assert.equal(shouldOpenInNewTab(url, BASE, true), false)
  assert.deepEqual(linkTargetProps(url, BASE, true), {})
}

// 8. Localhost port distinction (e.g. Corral on :8080 vs another local app on :3000).
assert.equal(isExternalLink('http://localhost:8080/agents', 'http://localhost:8080/'), false)
assert.equal(isExternalLink('http://localhost:3000/', 'http://localhost:8080/'), true)
assert.deepEqual(linkTargetProps('http://localhost:3000/', 'http://localhost:8080/'), {
  target: '_blank',
  rel: 'noopener noreferrer',
})

// 9. isAppRoutePath trailing slash and unknown path checks.
assert.equal(isAppRoutePath('/agents/'), true)
assert.equal(isAppRoutePath('/invite/tok-123'), true)
assert.equal(isAppRoutePath('/api/me'), false)
assert.equal(isAppRoutePath('/THIRD-PARTY-NOTICES.txt'), false)
assert.equal(isAppRoutePath('/hosts/1/edit/extra'), false)

// 10. isPlainPrimaryClick preserves Cmd/Ctrl/Shift/Alt+click and middle-click for native new-tab opening.
const click = (over = {}) => ({
  metaKey: false,
  ctrlKey: false,
  shiftKey: false,
  altKey: false,
  button: 0,
  defaultPrevented: false,
  ...over,
})
assert.equal(isPlainPrimaryClick(click()), true)
assert.equal(isPlainPrimaryClick(click({ metaKey: true })), false)
assert.equal(isPlainPrimaryClick(click({ ctrlKey: true })), false)
assert.equal(isPlainPrimaryClick(click({ shiftKey: true })), false)
assert.equal(isPlainPrimaryClick(click({ altKey: true })), false)
assert.equal(isPlainPrimaryClick(click({ button: 1 })), false) // middle click
assert.equal(isPlainPrimaryClick(click({ button: 2 })), false) // right click
assert.equal(isPlainPrimaryClick(click({ defaultPrevented: true })), false)

console.log('linkPolicy: all tests passed')
