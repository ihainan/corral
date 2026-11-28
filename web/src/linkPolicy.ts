// Pure URL classification and click-handling policy for Corral links.
// Kept free of DOM/browser globals so unit tests in web/scripts/ can run it directly.

export type LinkKind =
  | 'hash' // '#section' or empty — same document
  | 'internal-app' // same-origin Corral SPA route ('/', '/agents', '/t/abc', ...)
  | 'same-origin-other' // same-origin non-SPA endpoint ('/api/...', '/THIRD-PARTY-NOTICES.txt')
  | 'external-http' // cross-origin http(s) URL — always opens in a new tab
  | 'external-scheme' // mailto:, tel:, sms: — external protocol, handled by OS without spawning a blank tab
  | 'unsafe' // javascript:, data:, vbscript:, file:, malformed — never opened programmatically

export interface LinkInfo {
  kind: LinkKind
  /** True when the link points outside Corral (cross-origin http(s) or external protocol like mailto:/tel:). */
  external: boolean
  /** True when clicking should default to a new tab/window with rel="noopener noreferrer". */
  openInNewTab: boolean
  /** True for links safe to open. False for javascript:, data:, vbscript:, file:, or invalid URLs. */
  safe: boolean
  /** Path+search+hash for in-app SPA navigation when kind === 'internal-app'. */
  spaPath?: string
}

export interface ClickLike {
  metaKey: boolean
  ctrlKey: boolean
  shiftKey: boolean
  altKey?: boolean
  button: number
  defaultPrevented?: boolean
}

const EXTERNAL_OS_SCHEMES = new Set(['mailto:', 'tel:', 'sms:'])
const HTTP_SCHEMES = new Set(['http:', 'https:'])
const SCHEME_RE = /^([a-zA-Z][a-zA-Z\d+.-]*):/

// Matches Corral's client-side SPA routes (see web/src/app.tsx route()).
export function isAppRoutePath(pathname: string): boolean {
  const p = pathname.length > 1 && pathname.endsWith('/') ? pathname.slice(0, -1) : pathname
  if (
    p === '/' ||
    p === '/hosts' ||
    p === '/hosts/new' ||
    p === '/agents' ||
    p === '/t' ||
    p === '/keys' ||
    p === '/settings' ||
    p === '/recordings' ||
    p === '/admin/users' ||
    p === '/admin/audit' ||
    p === '/login' ||
    p === '/setup'
  ) {
    return true
  }
  if (/^\/hosts\/[^/]+\/edit$/.test(p)) return true
  if (/^\/[tw]\/[^/]+$/.test(p)) return true
  if (/^\/recordings\/[^/]+$/.test(p)) return true
  if (/^\/invite\/[^/]+$/.test(p)) return true
  return false
}

export function classifyLink(href: string, baseUrl: string): LinkInfo {
  const raw = href.trim()
  if (!raw || raw.startsWith('#')) {
    return { kind: 'hash', external: false, openInNewTab: false, safe: true }
  }
  if (/[\x00-\x1f\x7f]/.test(raw)) {
    return { kind: 'unsafe', external: false, openInNewTab: false, safe: false }
  }
  const schemeMatch = SCHEME_RE.exec(raw)
  if (schemeMatch) {
    const scheme = schemeMatch[1].toLowerCase() + ':'
    if (EXTERNAL_OS_SCHEMES.has(scheme)) {
      return { kind: 'external-scheme', external: true, openInNewTab: false, safe: true }
    }
    if (!HTTP_SCHEMES.has(scheme)) {
      return { kind: 'unsafe', external: false, openInNewTab: false, safe: false }
    }
  }
  let base: URL
  let parsed: URL
  try {
    base = new URL(baseUrl)
    parsed = new URL(raw, base)
  } catch {
    return { kind: 'unsafe', external: false, openInNewTab: false, safe: false }
  }
  if (!HTTP_SCHEMES.has(parsed.protocol)) {
    return { kind: 'unsafe', external: false, openInNewTab: false, safe: false }
  }
  if (parsed.origin !== base.origin) {
    return { kind: 'external-http', external: true, openInNewTab: true, safe: true }
  }
  if (isAppRoutePath(parsed.pathname)) {
    return {
      kind: 'internal-app',
      external: false,
      openInNewTab: false,
      safe: true,
      spaPath: parsed.pathname + parsed.search + parsed.hash,
    }
  }
  return { kind: 'same-origin-other', external: false, openInNewTab: false, safe: true }
}

export function isExternalLink(href: string, baseUrl: string): boolean {
  return classifyLink(href, baseUrl).external
}

export function shouldOpenInNewTab(href: string, baseUrl: string, forceNewTab = false): boolean {
  const info = classifyLink(href, baseUrl)
  if (!info.safe) return false
  if (info.openInNewTab) return true
  return forceNewTab && info.kind !== 'external-scheme'
}

export function linkTargetProps(
  href: string,
  baseUrl: string,
  forceNewTab = false,
): { target?: '_blank'; rel?: 'noopener noreferrer' } {
  return shouldOpenInNewTab(href, baseUrl, forceNewTab) ? { target: '_blank', rel: 'noopener noreferrer' } : {}
}

// Plain primary-button click with no modifiers: handled by in-app SPA routing.
// Cmd/Ctrl/Shift/Alt+click or middle-click returns false so the browser opens a new tab natively.
export function isPlainPrimaryClick(e: ClickLike): boolean {
  return !e.defaultPrevented && e.button === 0 && !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey
}
