import { api } from './api'

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
