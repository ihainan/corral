// Minimal service worker: makes the app installable and keeps the shell
// available offline. API calls and sockets always go to the network.
const SHELL = 'corral-shell-v2'
// The language picked in the app (written there, see i18n.ts); kept across
// shell versions.
const PREFS = 'corral-prefs'
// The newest alert shown for each tag: one that arrives late (pushes can
// overtake each other) does not take its place.
const ALERTS = 'corral-alerts'
const ALERTS_KEPT = 200

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(SHELL).then((c) => c.addAll(['/'])).then(() => self.skipWaiting()))
})

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== SHELL && k !== PREFS && k !== ALERTS).map((k) => caches.delete(k)))).then(() => self.clients.claim()),
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

// The language to show alerts in: the one picked in the app, else the device's.
async function chinese() {
  try {
    const r = await (await caches.open(PREFS)).match('/lang')
    if (r) return (await r.text()).startsWith('zh')
  } catch {
    /* fall back to the device language */
  }
  return (self.navigator.language || '').toLowerCase().startsWith('zh')
}

// Push: "an agent needs you" and similar alerts from the gateway, in both
// languages, one at a time in the order they came.
let pushes = Promise.resolve()
self.addEventListener('push', (e) => {
  let n = {}
  try {
    n = e.data ? e.data.json() : {}
  } catch {
    n = { title: 'Corral', body: e.data ? e.data.text() : '' }
  }
  const run = pushes.then(() => showAlert(n))
  pushes = run.catch(() => {})
  e.waitUntil(run)
})

// newest keeps the newest alert of a tag and returns what to show: this one,
// or (an older one, arriving late) the newer one again, quietly and without
// buttons; a push must show something. Kept in memory and in Cache Storage
// (the worker may be stopped between pushes), only what showing it again
// needs, for a day; the page clears it when the account changes.
const seen = new Map() // tag → the newest alert, while this worker runs
const KEEP_MS = 24 * 60 * 60 * 1000
async function newest(n) {
  if (!n.tag || typeof n.seq !== 'number') return n
  const keep = { title: n.title, title_zh: n.title_zh, body: n.body, body_zh: n.body_zh, tag: n.tag, url: n.url, seq: n.seq, at: Date.now() }
  let prev = seen.get(n.tag)
  try {
    const c = await caches.open(ALERTS)
    const key = '/tag/' + encodeURIComponent(n.tag)
    const r = await c.match(key)
    const stored = r ? await r.json() : null
    if (stored && typeof stored.seq === 'number' && Date.now() - (stored.at || 0) < KEEP_MS && (!prev || stored.seq > prev.seq)) prev = stored
    if (!(prev && prev.seq > n.seq)) {
      await c.put(key, new Response(JSON.stringify(keep)))
      const keys = await c.keys()
      for (const k of keys.slice(0, Math.max(0, keys.length - ALERTS_KEPT))) await c.delete(k)
    }
  } catch {
    /* no storage: what this worker remembers */
  }
  if (prev && prev.seq > n.seq) return { ...prev, quiet: true }
  seen.set(n.tag, keep)
  return n
}

async function showAlert(pushed) {
  const n = await newest(pushed)
  const zh = await chinese()
  return self.registration.showNotification((zh && n.title_zh) || n.title || 'Corral', {
    body: (zh && n.body_zh) || n.body || '',
    tag: n.tag || undefined,
    // A quiet one (an agent that stopped waiting) replaces without a sound.
    renotify: !!n.tag && !n.quiet,
    silent: !!n.quiet,
    icon: '/icons/icon-192.png',
    badge: '/icons/icon-192.png',
    // Buttons where the platform shows them (not iOS): an agent's choices.
    actions: Array.isArray(n.actions) && n.answer ? n.actions.map((a) => ({ action: a.action, title: (zh && a.title_zh) || a.title })) : [],
    data: { url: n.url || '/', answer: n.answer || null },
  })
}

self.addEventListener('notificationclick', (e) => {
  e.notification.close()
  const data = e.notification.data || {}
  const url = new URL(data.url || '/', self.location.origin)
  if (url.origin !== self.location.origin) return
  // A button: the choice is pressed in the pane, once, if the agent still
  // waits as the alert said and its screen is unchanged (the gateway checks
  // the alert's token); otherwise (or on any failure) the page opens on that
  // pane's answer, to look first.
  if (e.action && data.answer && /^[1-9]$/.test(e.action)) {
    e.waitUntil(
      fetch('/api/alerts/answer', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: data.answer, key: e.action }),
      })
        .then((res) => (res.ok ? null : openPage(url)))
        .catch(() => openPage(url)),
    )
    return
  }
  e.waitUntil(openPage(url))
})

// openPage shows an address of Corral in a window: one showing it already,
// one with terminal tabs, one elsewhere in Corral, or a new one.
function openPage(url) {
  return (
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((list) => {
      const path = (c) => new URL(c.url).pathname
      // A window already showing it comes to the front as it is.
      const same = list.find((c) => path(c) === url.pathname)
      if (same) {
        // What the address asks for (answering an agent) is passed on.
        if (url.search) same.postMessage({ t: 'open', url: url.href })
        return same.focus()
      }
      // A window with terminal tabs switches to it as a tab (asked, as only
      // the page knows whether it has tabs).
      const terms = list.filter((c) => path(c).startsWith('/t/'))
      return firstAccepting(terms, url.href).then((tabs) => {
        if (tabs) return tabs.focus()
        // Else a window that is not on a terminal page is reused; a terminal
        // page is never navigated away (its connection would end).
        const free = list.find((c) => 'navigate' in c && !path(c).startsWith('/t/'))
        if (free) return free.focus().then(() => free.navigate(url.href))
        return self.clients.openWindow(url.href)
      })
    })
  )
}

// The first of these windows that takes the address as a tab, or null. Each
// is asked in turn and given a moment to answer.
async function firstAccepting(clients, href) {
  for (const c of clients) {
    const ok = await new Promise((resolve) => {
      const ch = new MessageChannel()
      const timer = setTimeout(() => resolve(false), 500)
      ch.port1.onmessage = (e) => {
        clearTimeout(timer)
        resolve(e.data === true)
      }
      c.postMessage({ t: 'tab', url: href }, [ch.port2])
    })
    if (ok) return c
  }
  return null
}
