// A notification tapped while a window already shows its page: the service
// worker brings that window to the front and passes the address on (it may
// ask for more, such as answering an agent: ?reply=<pane>). The address is
// the one place this is kept, as it survives what memory does not on the way
// (being sent to sign in, a reload, the page still starting): it is written
// there, and the page reads it from there. Listening starts as the app loads.

const listeners = new Set<() => void>()

if ('serviceWorker' in navigator) {
  navigator.serviceWorker.addEventListener('message', (e: MessageEvent) => {
    const d = e.data as { t?: string; url?: string } | null
    if (d?.t !== 'open' || typeof d.url !== 'string') return
    const u = new URL(d.url, location.origin)
    if (u.origin !== location.origin) return
    if (location.pathname === u.pathname) {
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

// onOpenRequest calls f when such a request has been written into the
// address of the page the window shows.
export function onOpenRequest(f: () => void): () => void {
  listeners.add(f)
  return () => {
    listeners.delete(f)
  }
}
