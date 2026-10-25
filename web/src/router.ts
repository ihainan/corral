import { useEffect, useState } from 'react'

// A minimal history router: the app has a handful of flat routes.

const listeners = new Set<() => void>()

// state: kept with the history entry (history.state), as the agents page
// marks a conversation opened from its list.
export function navigate(to: string, replace = false, state: unknown = null) {
  if (to === location.pathname + location.search) return
  if (replace) history.replaceState(state, '', to)
  else history.pushState(state, '', to)
  listeners.forEach((l) => l())
}

window.addEventListener('popstate', () => listeners.forEach((l) => l()))

export function usePath(): string {
  const [path, setPath] = useState(location.pathname)
  useEffect(() => {
    const l = () => setPath(location.pathname)
    listeners.add(l)
    return () => {
      listeners.delete(l)
    }
  }, [])
  return path
}

// match('/hosts/:id/edit', '/hosts/3/edit') → { id: '3' }
export function match(pattern: string, path: string): Record<string, string> | null {
  const p = pattern.split('/').filter(Boolean)
  const s = path.split('/').filter(Boolean)
  if (p.length !== s.length) return null
  const out: Record<string, string> = {}
  for (let i = 0; i < p.length; i++) {
    if (p[i].startsWith(':')) out[p[i].slice(1)] = decodeURIComponent(s[i])
    else if (p[i] !== s[i]) return null
  }
  return out
}

export function linkHandler(to: string) {
  return (e: React.MouseEvent) => {
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return
    e.preventDefault()
    navigate(to)
  }
}
