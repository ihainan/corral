import { navigate } from './router'

// A notification tapped while a window already shows its page: the service
// worker brings that window to the front and passes the address on (it may
// ask for more, such as answering an agent: ?reply=<pane>). The address is
// the one place this is kept, as it survives what memory does not on the way
// (being sent to sign in, a reload, the page still starting): it is written
// there, and the page reads it from there. Listening starts as the app loads.

const listeners = new Set<() => void>()

// A page with terminal tabs takes an address of another terminal, or the
// agents page's, as a tab: the service worker asks before it opens a window
// for it. A page that says nothing of it (the hosts page, the agents page on
// a phone, settings) goes there within the app, unless a dialog is up; a
// terminal's or a watched session's page without a say keeps its
// connection, the sign-in page what is typed in it.
let tabTaker: ((url: URL) => boolean) | null = null
export function takeTabs(f: ((url: URL) => boolean) | null) {
  tabTaker = f
}
export const agentsAddress = (u: URL) => u.pathname === '/agents'
const takeElsewhere = (u: URL) => {
  if (/^\/(t|w)\//.test(location.pathname) || location.pathname === '/login' || document.querySelector('[aria-modal="true"]')) return false
  if (!agentsAddress(u) && !/^\/t\/[0-9a-f]+$/.test(u.pathname)) return false
  navigate(u.pathname + u.search)
  return true
}

if ('serviceWorker' in navigator) {
  navigator.serviceWorker.addEventListener('message', (e: MessageEvent) => {
    const d = e.data as { t?: string; url?: string } | null
    if (d?.t === 'tab' && typeof d.url === 'string') {
      const u = new URL(d.url, location.origin)
      e.ports[0]?.postMessage(u.origin === location.origin && !!(tabTaker ?? takeElsewhere)(u))
      return
    }
    if (d?.t !== 'open' || typeof d.url !== 'string') return
    const u = new URL(d.url, location.origin)
    if (u.origin !== location.origin) return
    if (location.pathname === u.pathname) {
      // (A dialog is up — a fork being filled in, say: the page is not taken
      // from under it, nor its address changed; it is told one is there, and
      // takes it when the person says so — see takeHeld. Review B18.)
      if (u.pathname === '/agents' && document.querySelector('[aria-modal="true"]')) {
        held = u
        heldListeners.forEach((l) => l())
        return
      }
      history.replaceState(history.state, '', u.pathname + u.search)
    } else if (location.pathname === '/login') {
      // Sent to sign in first: where it returns to carries it.
      const q = new URLSearchParams(location.search)
      const next = q.get('next')
      if (!next || new URL(next, location.origin).pathname !== u.pathname) return
      q.set('next', u.pathname + u.search)
      history.replaceState(history.state, '', '/login?' + q.toString())
      return
    } else return
    listeners.forEach((l) => l())
  })
}

// A request held back while a dialog was up (see above): the latest only.
let held: URL | null = null
const heldListeners = new Set<() => void>()
export const heldOpen = () => held
// onHeldOpen calls f when one is held, or let go of (taken or dropped).
export function onHeldOpen(f: () => void): () => void {
  heldListeners.add(f)
  return () => {
    heldListeners.delete(f)
  }
}
// takeHeld does what was held: its address written, the page told. drop:
// let go of without.
export function takeHeld(drop = false) {
  const u = held
  held = null
  heldListeners.forEach((l) => l())
  if (!u || drop || location.pathname !== u.pathname) return
  history.replaceState(history.state, '', u.pathname + u.search)
  listeners.forEach((l) => l())
}

// onOpenRequest calls f when such a request has been written into the
// address of the page the window shows.
export function onOpenRequest(f: () => void): () => void {
  listeners.add(f)
  return () => {
    listeners.delete(f)
  }
}
