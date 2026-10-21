import { api } from './api'
import { local } from './storage'

export type PushState = 'unsupported' | 'needs-home-screen' | 'denied' | 'off' | 'on'

const isIOS = /iPhone|iPad|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1)
const standalone = window.matchMedia('(display-mode: standalone)').matches || (navigator as any).standalone === true

export async function pushState(): Promise<PushState> {
  if (!('serviceWorker' in navigator) || !('PushManager' in window) || !('Notification' in window)) {
    return isIOS && !standalone ? 'needs-home-screen' : 'unsupported'
  }
  if (Notification.permission === 'denied') return 'denied'
  const reg = await navigator.serviceWorker.getRegistration()
  const sub = await reg?.pushManager.getSubscription()
  return sub ? 'on' : 'off'
}

function keyBytes(b64: string): Uint8Array {
  const pad = '='.repeat((4 - (b64.length % 4)) % 4)
  const raw = atob((b64 + pad).replace(/-/g, '+').replace(/_/g, '/'))
  return Uint8Array.from(raw, (c) => c.charCodeAt(0))
}

// enablePush must be called from a tap: iOS only asks for permission then.
// The fields the gateway keeps (toJSON() also has expirationTime, which
// older gateways refused).
function subBody(sub: PushSubscription) {
  const j = sub.toJSON()
  return { endpoint: j.endpoint, keys: { p256dh: j.keys?.p256dh, auth: j.keys?.auth } }
}

export async function enablePush(publicKey: string): Promise<PushState> {
  const perm = await Notification.requestPermission()
  if (perm !== 'granted') return perm === 'denied' ? 'denied' : 'off'
  const reg = (await navigator.serviceWorker.getRegistration()) ?? (await navigator.serviceWorker.register('/sw.js'))
  await navigator.serviceWorker.ready
  const existing = await reg.pushManager.getSubscription()
  const sub = existing ?? (await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(publicKey) as BufferSource }))
  try {
    await api('POST', '/api/push/subscriptions', subBody(sub))
  } catch (e) {
    // A subscription the server does not know about would read as "on" here
    // while no alert ever arrives, so drop the one we just made.
    if (!existing) await sub.unsubscribe().catch(() => {})
    throw e
  }
  return 'on'
}

// syncPush registers this device's existing subscription to whoever is
// signed in now. The server keys subscriptions by endpoint, so after an
// account switch this moves the device to the new account. Returns false when
// the server did not take it.
export async function syncPush(): Promise<boolean> {
  if (!('serviceWorker' in navigator) || !('PushManager' in window)) return true
  try {
    const reg = await navigator.serviceWorker.getRegistration()
    const sub = await reg?.pushManager.getSubscription()
    if (!sub) return true
    await api('POST', '/api/push/subscriptions', subBody(sub))
    return true
  } catch {
    return false
  }
}

export async function disablePush(): Promise<void> {
  const reg = await navigator.serviceWorker.getRegistration()
  const sub = await reg?.pushManager.getSubscription()
  if (!sub) return
  await api('DELETE', '/api/push/subscriptions', { endpoint: sub.endpoint }).catch(() => {})
  await sub.unsubscribe()
}

// offerAlerts asks, now and then, whether this device should get alerts:
// when an agent is first seen here and the browser has not been asked yet
// (permission neither granted nor refused). The prompt itself must come from
// a tap (iOS), so what is shown is a toast whose button asks. At most three
// times, a week apart; the setting stays in the settings page.
const OFFER_KEY = 'corral.alertOffer' // "<times>:<last ms>"
const OFFER_EVERY = 7 * 24 * 3600 * 1000
let offeredNow = false // once per page load
export async function offerAlerts(show: (ask: () => Promise<PushState>) => void) {
  if (offeredNow || !('Notification' in window) || Notification.permission !== 'default') return
  const [times, last] = (local.get(OFFER_KEY) ?? '0:0').split(':').map(Number)
  if (times >= 3 || Date.now() - last < OFFER_EVERY) return
  offeredNow = true
  // Counted before anything is awaited (another tab may be about to offer
  // too), given back if it turns out not to be offered.
  const before = local.get(OFFER_KEY)
  local.set(OFFER_KEY, `${times + 1}:${Date.now()}`)
  const giveBack = () => (before === null ? local.remove(OFFER_KEY) : local.set(OFFER_KEY, before))
  const st = await pushState().catch((): PushState => 'unsupported')
  const info = st === 'off' ? await api<{ enabled: boolean; public_key?: string }>('GET', '/api/push').catch(() => null) : null
  if (!info?.enabled || !info.public_key) return giveBack()
  const key = info.public_key
  show(() => enablePush(key))
}
