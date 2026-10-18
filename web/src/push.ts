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
export async function enablePush(publicKey: string): Promise<PushState> {
  const perm = await Notification.requestPermission()
  if (perm !== 'granted') return perm === 'denied' ? 'denied' : 'off'
  const reg = (await navigator.serviceWorker.getRegistration()) ?? (await navigator.serviceWorker.register('/sw.js'))
  await navigator.serviceWorker.ready
  const sub = (await reg.pushManager.getSubscription()) ?? (await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(publicKey) as BufferSource }))
  await api('POST', '/api/push/subscriptions', sub.toJSON())
  return 'on'
}

export async function disablePush(): Promise<void> {
  const reg = await navigator.serviceWorker.getRegistration()
  const sub = await reg?.pushManager.getSubscription()
  if (!sub) return
  await api('DELETE', '/api/push/subscriptions', { endpoint: sub.endpoint }).catch(() => {})
  await sub.unsubscribe()
}
