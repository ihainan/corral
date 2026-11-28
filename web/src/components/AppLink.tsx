import type { AnchorHTMLAttributes, MouseEvent } from 'react'
import { classifyLink, isPlainPrimaryClick, linkTargetProps, type ClickLike } from '../linkPolicy'
import { navigate } from '../router'

export interface AppLinkProps extends Omit<AnchorHTMLAttributes<HTMLAnchorElement>, 'href'> {
  href: string
  /** Force opening in a new tab with rel="noopener noreferrer" (e.g. standalone static docs or external apps). */
  newTab?: boolean
  /** Disable SPA interception for same-origin non-SPA endpoints (e.g. OIDC redirect or file downloads). */
  native?: boolean
}

// Unified link component for Corral:
// - Cross-origin http(s) links (and explicit newTab links) open in a new tab with rel="noopener noreferrer".
// - Same-origin Corral SPA routes stay in the current window on plain left-click while preserving native
//   Cmd/Ctrl+click and middle-click to open in a new tab.
// - External OS schemes (mailto:, tel:) keep default browser handling without spawning a stray blank tab.
export function AppLink({ href, newTab = false, native = false, target, rel, onClick, ...rest }: AppLinkProps) {
  const baseUrl = window.location.href
  const info = classifyLink(href, baseUrl)
  const policyAttrs = linkTargetProps(href, baseUrl, newTab)
  const finalTarget = target ?? policyAttrs.target
  const finalRel = rel ?? policyAttrs.rel

  const handleClick = (e: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(e)
    if (e.defaultPrevented) return
    if (finalTarget === '_blank' || native) return
    if (info.kind === 'internal-app' && info.spaPath && isPlainPrimaryClick(e)) {
      e.preventDefault()
      navigate(info.spaPath)
    }
  }

  return <a {...rest} href={href} target={finalTarget} rel={finalRel} onClick={handleClick} />
}

// Opens a validated http(s) or same-origin URL in a new tab with noopener,noreferrer.
export function openInNewTab(href: string): Window | null {
  const info = classifyLink(href, window.location.href)
  if (!info.safe || info.kind === 'external-scheme') return null
  return window.open(href, '_blank', 'noopener,noreferrer')
}

// Unified handler for terminal links (WebLinksAddon and OSC 8 linkHandler):
// - Only allows http(s) URLs.
// - External URLs always open in a new tab with noopener,noreferrer.
// - Same-origin Corral SPA URLs navigate in-place on a plain click, or open in a new tab on Cmd/Ctrl/middle click.
export function openTerminalLink(uri: string, e?: ClickLike): void {
  if (!/^https?:\/\//i.test(uri.trim())) return
  const info = classifyLink(uri, window.location.href)
  if (!info.safe) return
  if (info.kind === 'internal-app' && info.spaPath && (!e || isPlainPrimaryClick(e))) {
    navigate(info.spaPath)
    return
  }
  window.open(uri, '_blank', 'noopener,noreferrer')
}
