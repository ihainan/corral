// Minimal service worker: makes the app installable and keeps the shell
// available offline. API calls and sockets always go to the network.
const SHELL = 'corral-shell-v2'

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(SHELL).then((c) => c.addAll(['/'])).then(() => self.skipWaiting()))
})

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== SHELL).map((k) => caches.delete(k)))).then(() => self.clients.claim()),
  )
})

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url)
  if (e.request.method !== 'GET' || url.origin !== location.origin || url.pathname.startsWith('/api/')) return
  if (url.pathname.startsWith('/assets/')) {
    // Hashed file names never change: cache first.
    e.respondWith(
      caches.open(SHELL).then(async (c) => {
        const hit = await c.match(e.request)
        if (hit) return hit
        const res = await fetch(e.request)
        if (res.ok) c.put(e.request, res.clone())
        return res
      }),
    )
    return
  }
  if (e.request.mode === 'navigate') {
    // Network first so deploys show up immediately; the cached shell is the fallback.
    e.respondWith(
      fetch(e.request)
        .then((res) => {
          const copy = res.clone()
          caches.open(SHELL).then((c) => c.put('/', copy))
          return res
        })
        .catch(() => caches.match('/')),
    )
  }
})

// Push: "an agent needs you" and similar alerts from the gateway.
self.addEventListener('push', (e) => {
  let n = {}
  try {
    n = e.data ? e.data.json() : {}
  } catch {
    n = { title: 'Corral', body: e.data ? e.data.text() : '' }
  }
  e.waitUntil(
    self.registration.showNotification(n.title || 'Corral', {
      body: n.body || '',
      tag: n.tag || undefined,
      renotify: !!n.tag,
      silent: !!n.quiet,
      icon: '/icons/icon-192.png',
      badge: '/icons/icon-192.png',
      data: { url: n.url || '/' },
    }),
  )
})

self.addEventListener('notificationclick', (e) => {
  e.notification.close()
  const url = new URL((e.notification.data && e.notification.data.url) || '/', self.location.origin)
  if (url.origin !== self.location.origin) return
  e.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((list) => {
      const path = (c) => new URL(c.url).pathname
      // A window already showing it comes to the front as it is.
      const same = list.find((c) => path(c) === url.pathname)
      if (same) return same.focus()
      // Else a window that is not on a terminal page is reused; a terminal
      // page is never navigated away (its composer draft would be lost).
      const free = list.find((c) => 'navigate' in c && !path(c).startsWith('/t/'))
      if (free) return free.focus().then(() => free.navigate(url.href))
      return self.clients.openWindow(url.href)
    }),
  )
})
